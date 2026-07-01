//go:build windows

// This file is the real Windows half of the OS capture/playback boundary
// described in audio.go: a WASAPI (Windows Audio Session API) Source/Sink pair
// that a live Sender/Receiver pipeline can use as a drop-in replacement for
// SineSource/BufferSink.
//
// Library choice: github.com/moutend/go-wca, a pure-Go binding to WASAPI's COM
// interfaces (IMMDeviceEnumerator, IAudioClient, IAudioCaptureClient,
// IAudioRenderClient, ...) built on github.com/go-ole/go-ole. Both are plain
// `syscall`-based Go — no cgo, no C toolchain.
//
// This matters for consistency with the rest of the package (and the rest of
// this repo's Go side, which is pure Go outside the one deliberate
// core/cabi cgo boundary to the Rust capability kernel, see CLAUDE.md). The
// alternative considered was github.com/gen2brain/malgo (Go bindings to the
// miniaudio C library): it also has a real WASAPI backend, but it requires
// cgo and therefore a C toolchain at build time. This machine has no gcc/cc on
// PATH (verified: `where gcc`/`where cc` found nothing, and `go env CC` names
// a compiler that isn't actually installed), so a cgo-based backend would not
// even compile here, let alone run — go-wca was the only backend of the two
// that was actually buildable and testable in this environment. It also keeps
// this package's zero-cgo posture intact: adding a cgo dependency here would
// mean this leaf package (deliberately import-nothing-else, see audio.go's
// package doc) suddenly needs a C compiler to build on Windows, which is a
// meaningfully bigger ask than what every other file in this package needs.
//
// A note on target architecture: this file was developed and verified against
// both windows/amd64 and windows/386 builds. The vtable call marshaling
// go-wca (and the syscall package generally) uses on 32-bit Windows truncates
// 64-bit-by-value COM parameters (e.g. IAudioClient.Initialize's
// REFERENCE_TIME buffer-duration argument) because `uintptr` is only 32 bits
// there. That is a real, load-bearing limitation of the pure-Go COM plumbing
// on windows/386 — confirmed by hand: IAudioClient.Initialize fails on
// windows/386 with a well-formed request that succeeds unmodified on
// windows/amd64. It does not affect this repo in practice: the project's own
// release matrix and CI (build/release.ps1, .github/workflows/release.yml,
// .github/workflows/ci.yml windows-latest runner) only ever target
// windows/amd64 and windows/arm64, never windows/386, so this is noted here
// for the record rather than worked around.
//
// A second real hardware constraint discovered while verifying this file on
// this machine's actual audio device: WASAPI shared-mode streams frequently
// reject IAudioClient.Initialize for a channel count that does not match the
// endpoint's mix format (confirmed live: this device's mix format is 2-channel
// 48kHz, and requesting a straight mono 16-bit PCM stream fails Initialize
// with AUDCLNT_E_UNSUPPORTED_FORMAT even though IsFormatSupported's own
// closest-match output disagrees). The fix used here is the standard one: open
// the device at its native channel count (from IAudioClient.GetMixFormat) and
// convert between that and the audio.Format the Source/Sink contract exposes
// in software (mono<->stereo). Sample RATE mismatches are not resampled — if
// the device's native rate differs from the requested Format's, this package
// fails loudly (ErrSampleRateMismatch) rather than silently distorting audio
// with naive resampling; see openNegotiatedClient.
package audio

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

// wasapiBitsPerSample is the sample width this package always requests from
// WASAPI: audio.Frame is 16-bit signed PCM (see audio.go's Format doc), so the
// device is opened in that exact bit depth rather than the engine's internal
// float32 mix format, keeping the wire/PCM representation identical end to end
// from capture to network to playback (channel count is negotiated
// separately; see openNegotiatedClient).
const wasapiBitsPerSample = 16

// wasapiBufferDuration is the WASAPI shared-mode buffer size requested from
// IAudioClient.Initialize, in 100ns units (REFERENCE_TIME). 200ms gives the
// capture/render loop comfortable headroom to be scheduled without underrun,
// while staying well inside typical device buffer limits.
const wasapiBufferDuration = wca.REFERENCE_TIME(200 * 10 * 1000) // 200ms

// ErrNoAudioDevice is returned when WASAPI has no capture or render endpoint
// at all (e.g. a headless/sandboxed machine with no audio driver). Per
// CLAUDE.md "maturity honesty" (mirroring core/runtime/src/gpu.rs's
// GpuError::NoAdapter for the analogous case in the GPU subsystem), this
// package fails loudly instead of silently generating or discarding audio
// when there is no real device to back the Source/Sink contract.
var ErrNoAudioDevice = errors.New("audio: no WASAPI endpoint available on this system")

// ErrSampleRateMismatch is returned when the requested Format's sample rate
// does not match the WASAPI endpoint's native mix rate. This package
// negotiates channel count in software (see negotiatedFormat) but
// deliberately does not resample, since a naive resampler would silently
// degrade audio quality/pitch — an honest failure here is preferable to that.
var ErrSampleRateMismatch = errors.New("audio: requested sample rate does not match the WASAPI endpoint's native rate")

// negotiatedFormat pairs the audio.Format this package's Source/Sink contract
// exposes with the WAVEFORMATEX actually opened on the device (which may have
// a different channel count — see the file doc comment).
type negotiatedFormat struct {
	appFormat    Format // what Source.Format()/Sink.Format() reports
	deviceFormat wca.WAVEFORMATEX
}

func (n negotiatedFormat) deviceChannels() int { return int(n.deviceFormat.NChannels) }

// waveFormat builds the 16-bit-PCM WAVEFORMATEX WASAPI is asked to open the
// stream in, at the given channel count / sample rate.
func waveFormat(channels uint16, rate uint32) wca.WAVEFORMATEX {
	blockAlign := channels * (wasapiBitsPerSample / 8)
	return wca.WAVEFORMATEX{
		WFormatTag:      wca.WAVE_FORMAT_PCM,
		NChannels:       channels,
		NSamplesPerSec:  rate,
		WBitsPerSample:  wasapiBitsPerSample,
		NBlockAlign:     blockAlign,
		NAvgBytesPerSec: rate * uint32(blockAlign),
	}
}

// Every method that touches a *wca COM object must run its whole lifetime —
// construction, use, and Release — pinned to one OS thread that has called
// CoInitializeEx: WASAPI's COM interfaces are only valid to call from such a
// thread, and a bare goroutine can otherwise hop OS threads between calls.
// wasapiCaptureSource / wasapiPlaybackSink each run their entire WASAPI
// lifetime on one dedicated goroutine locked to its OS thread (runtime.
// LockOSThread) with COM initialized on it for exactly that reason; see their
// constructors below.

// defaultEndpoint resolves the default capture or render IMMDevice. eDataFlow
// is wca.ECapture or wca.ERender. It returns ErrNoAudioDevice (wrapping the
// underlying HRESULT) if WASAPI reports no such endpoint exists at all —
// distinct from a device existing but being busy/misconfigured, which returns
// the raw WASAPI error so the caller can see the real HRESULT.
func defaultEndpoint(eDataFlow uint32) (*wca.IMMDeviceEnumerator, *wca.IMMDevice, error) {
	var mmde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &mmde); err != nil {
		return nil, nil, fmt.Errorf("audio: WASAPI CoCreateInstance(MMDeviceEnumerator): %w", err)
	}

	// Confirm at least one active endpoint of the requested kind exists so we
	// can return the specific, honest ErrNoAudioDevice rather than whatever
	// generic HRESULT GetDefaultAudioEndpoint happens to produce when there is
	// truly no hardware (e.g. a headless CI/sandbox image).
	var coll *wca.IMMDeviceCollection
	if err := mmde.EnumAudioEndpoints(eDataFlow, wca.DEVICE_STATE_ACTIVE, &coll); err != nil {
		mmde.Release()
		return nil, nil, fmt.Errorf("audio: WASAPI EnumAudioEndpoints: %w", err)
	}
	var count uint32
	countErr := coll.GetCount(&count)
	coll.Release()
	if countErr != nil {
		mmde.Release()
		return nil, nil, fmt.Errorf("audio: WASAPI EnumAudioEndpoints.GetCount: %w", countErr)
	}
	if count == 0 {
		mmde.Release()
		return nil, nil, ErrNoAudioDevice
	}

	var device *wca.IMMDevice
	if err := mmde.GetDefaultAudioEndpoint(eDataFlow, wca.EConsole, &device); err != nil {
		mmde.Release()
		return nil, nil, fmt.Errorf("audio: WASAPI GetDefaultAudioEndpoint: %w", err)
	}
	return mmde, device, nil
}

// openNegotiatedClient activates an IAudioClient on device and initializes it
// in shared mode as 16-bit PCM at f's sample rate but the endpoint's native
// channel count (queried via GetMixFormat) — see the file doc comment for why
// the caller's exact channel count is not always accepted. It returns
// ErrSampleRateMismatch without calling Initialize at all if the endpoint's
// native rate differs from f.SampleRate, since this package does not
// resample.
func openNegotiatedClient(device *wca.IMMDevice, f Format, streamFlags uint32) (*wca.IAudioClient, negotiatedFormat, uint32, error) {
	var ac *wca.IAudioClient
	if err := device.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &ac); err != nil {
		return nil, negotiatedFormat{}, 0, fmt.Errorf("audio: WASAPI IMMDevice.Activate(IAudioClient): %w", err)
	}

	var mix *wca.WAVEFORMATEX
	if err := ac.GetMixFormat(&mix); err != nil {
		ac.Release()
		return nil, negotiatedFormat{}, 0, fmt.Errorf("audio: WASAPI IAudioClient.GetMixFormat: %w", err)
	}
	if mix.NSamplesPerSec != f.SampleRate {
		rate := mix.NSamplesPerSec
		ac.Release()
		return nil, negotiatedFormat{}, 0, fmt.Errorf("%w: requested %d Hz, device native rate is %d Hz", ErrSampleRateMismatch, f.SampleRate, rate)
	}

	wfx := waveFormat(mix.NChannels, f.SampleRate)
	if err := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, streamFlags, wasapiBufferDuration, 0, &wfx, nil); err != nil {
		ac.Release()
		return nil, negotiatedFormat{}, 0, fmt.Errorf("audio: WASAPI IAudioClient.Initialize (rate=%d deviceChannels=%d): %w", f.SampleRate, mix.NChannels, err)
	}
	var bufferFrames uint32
	if err := ac.GetBufferSize(&bufferFrames); err != nil {
		ac.Release()
		return nil, negotiatedFormat{}, 0, fmt.Errorf("audio: WASAPI IAudioClient.GetBufferSize: %w", err)
	}
	nf := negotiatedFormat{appFormat: f, deviceFormat: wfx}
	return ac, nf, bufferFrames, nil
}

// downmixToApp converts device-native-channel interleaved PCM (framesAvailable
// frames) down/up to the app's requested channel count, appending the result
// to out. Mono<->stereo is the common case (device is nearly always stereo;
// this package's Format is frequently mono for a voice-chat-style stream): a
// stereo->mono downmix averages L+R, a mono->stereo upmix duplicates the
// sample to both channels. Equal channel counts are a straight copy.
func downmixToApp(dst []int16, src []int16, deviceChannels, appChannels int) []int16 {
	frames := len(src) / deviceChannels
	switch {
	case deviceChannels == appChannels:
		return append(dst, src...)
	case appChannels == 1: // downmix any device channel count to mono by averaging
		for i := 0; i < frames; i++ {
			var sum int32
			base := i * deviceChannels
			for c := 0; c < deviceChannels; c++ {
				sum += int32(src[base+c])
			}
			dst = append(dst, int16(sum/int32(deviceChannels)))
		}
		return dst
	case deviceChannels == 1: // upmix mono device to N app channels by duplication
		for i := 0; i < frames; i++ {
			v := src[i]
			for c := 0; c < appChannels; c++ {
				dst = append(dst, v)
			}
		}
		return dst
	default:
		// Uncommon (e.g. device=4 app=2): duplicate/average pairwise as a
		// best-effort mapping rather than failing outright.
		for i := 0; i < frames; i++ {
			base := i * deviceChannels
			for c := 0; c < appChannels; c++ {
				dst = append(dst, src[base+(c%deviceChannels)])
			}
		}
		return dst
	}
}

// upmixFromApp converts app-channel interleaved PCM (one Frame's samples) to
// the device's native channel count for playback, the inverse of
// downmixToApp.
func upmixFromApp(src []int16, appChannels, deviceChannels int) []int16 {
	if appChannels == deviceChannels {
		return src
	}
	frames := len(src) / appChannels
	out := make([]int16, 0, frames*deviceChannels)
	switch {
	case deviceChannels == 1: // downmix app to mono device by averaging
		for i := 0; i < frames; i++ {
			var sum int32
			base := i * appChannels
			for c := 0; c < appChannels; c++ {
				sum += int32(src[base+c])
			}
			out = append(out, int16(sum/int32(appChannels)))
		}
	case appChannels == 1: // upmix mono app to N device channels by duplication
		for i := 0; i < frames; i++ {
			v := src[i]
			for c := 0; c < deviceChannels; c++ {
				out = append(out, v)
			}
		}
	default:
		for i := 0; i < frames; i++ {
			base := i * appChannels
			for c := 0; c < deviceChannels; c++ {
				out = append(out, src[base+(c%appChannels)])
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Capture (microphone) — wasapiCaptureSource implements Source.
// ---------------------------------------------------------------------------

// captureCmd is a request sent to the capture goroutine's COM-pinned loop.
type captureCmd struct {
	ctx   context.Context
	reply chan captureReply
}

type captureReply struct {
	frame Frame
	err   error
}

// wasapiCaptureSource is a real Source backed by a WASAPI capture (microphone)
// endpoint. All COM/WASAPI calls happen on one dedicated, COM-initialized OS
// thread; ReadFrame hands requests to that thread and waits for a reply, so
// wasapiCaptureSource itself is safe to call from any goroutine the way the
// Source interface requires.
type wasapiCaptureSource struct {
	format Format

	reqCh   chan captureCmd
	closeCh chan struct{}
	doneCh  chan struct{}

	closeOnce sync.Once
}

// NewOSCaptureSource opens the system default microphone via WASAPI in shared
// mode at f's sample rate as 16-bit PCM, negotiating the device's native
// channel count (see the file doc comment) and converting to f.Channels in
// software. It returns ErrNoAudioDevice if WASAPI reports no capture endpoint
// exists at all (a plausible, honest outcome in a headless/sandboxed
// environment), or ErrSampleRateMismatch if the device's native rate differs
// from f.SampleRate — rather than silently returning a Source that fabricates
// or mis-resamples audio.
func NewOSCaptureSource(f Format) (Source, error) {
	if f.Channels == 0 {
		f.Channels = 1
	}
	if f.SampleRate == 0 {
		return nil, fmt.Errorf("audio: NewOSCaptureSource: format has zero SampleRate")
	}

	type initResult struct{ err error }
	initDone := make(chan initResult, 1)

	src := &wasapiCaptureSource{
		format:  f,
		reqCh:   make(chan captureCmd),
		closeCh: make(chan struct{}),
		doneCh:  make(chan struct{}),
	}

	go func() {
		defer close(src.doneCh)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err == nil {
			defer ole.CoUninitialize()
		}

		mmde, device, err := defaultEndpoint(wca.ECapture)
		if err != nil {
			initDone <- initResult{err: err}
			return
		}
		defer mmde.Release()
		defer device.Release()

		ac, nf, _, err := openNegotiatedClient(device, f, 0)
		if err != nil {
			initDone <- initResult{err: err}
			return
		}
		defer ac.Release()

		var acc *wca.IAudioCaptureClient
		if err := ac.GetService(wca.IID_IAudioCaptureClient, &acc); err != nil {
			initDone <- initResult{err: fmt.Errorf("audio: WASAPI IAudioClient.GetService(IAudioCaptureClient): %w", err)}
			return
		}
		defer acc.Release()

		if err := ac.Start(); err != nil {
			initDone <- initResult{err: fmt.Errorf("audio: WASAPI IAudioClient.Start: %w", err)}
			return
		}
		defer ac.Stop()

		initDone <- initResult{}

		deviceChannels := nf.deviceChannels()
		appChannels := int(f.Channels)
		wantSamples := f.SamplesPerPacket()
		pending := make([]int16, 0, wantSamples*2)

		for {
			select {
			case <-src.closeCh:
				return
			case cmd := <-src.reqCh:
				frame, err := captureLoopReadFrame(cmd.ctx, acc, deviceChannels, appChannels, wantSamples, &pending, src.closeCh)
				select {
				case cmd.reply <- captureReply{frame: frame, err: err}:
				case <-src.closeCh:
					return
				}
				if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					return
				}
			}
		}
	}()

	res := <-initDone
	if res.err != nil {
		// The init goroutine returns immediately after reporting the error
		// (nothing left listening on reqCh/closeCh); just let it exit.
		<-src.doneCh
		return nil, res.err
	}
	return src, nil
}

// captureLoopReadFrame pulls WASAPI packets (in the device's native channel
// layout), downmixing each into the app's channel count and accumulating into
// *pending, until at least wantSamples int16 values are available, then
// returns exactly one Frame's worth and keeps any remainder buffered for the
// next call. It blocks (polling GetNextPacketSize) until enough data is
// available or ctx is cancelled / stop is closed.
func captureLoopReadFrame(ctx context.Context, acc *wca.IAudioCaptureClient, deviceChannels, appChannels, wantSamples int, pending *[]int16, stop <-chan struct{}) (Frame, error) {
	for len(*pending) < wantSamples {
		if err := ctx.Err(); err != nil {
			return Frame{}, err
		}
		select {
		case <-stop:
			return Frame{}, ErrOSAudioUnavailable
		default:
		}

		var packetLength uint32
		if err := acc.GetNextPacketSize(&packetLength); err != nil {
			return Frame{}, fmt.Errorf("audio: WASAPI IAudioCaptureClient.GetNextPacketSize: %w", err)
		}
		if packetLength == 0 {
			// No data ready yet; yield briefly rather than busy-spinning. A real
			// deployment would use IAudioClient.SetEventHandle + WaitForSingleObject
			// for a zero-poll wakeup; a short sleep is a simple, correct-enough
			// substitute that keeps this file's syscall surface small.
			if !sleepOrDone(ctx, stop) {
				return Frame{}, ctx.Err()
			}
			continue
		}

		var data *byte
		var framesAvailable, flags uint32
		if err := acc.GetBuffer(&data, &framesAvailable, &flags, nil, nil); err != nil {
			return Frame{}, fmt.Errorf("audio: WASAPI IAudioCaptureClient.GetBuffer: %w", err)
		}
		if framesAvailable > 0 {
			n := int(framesAvailable) * deviceChannels
			if flags&wca.AUDCLNT_BUFFERFLAGS_SILENT != 0 || data == nil {
				*pending = append(*pending, make([]int16, int(framesAvailable)*appChannels)...)
			} else {
				samples := unsafe.Slice((*int16)(unsafe.Pointer(data)), n)
				*pending = downmixToApp(*pending, samples, deviceChannels, appChannels)
			}
		}
		if err := acc.ReleaseBuffer(framesAvailable); err != nil {
			return Frame{}, fmt.Errorf("audio: WASAPI IAudioCaptureClient.ReleaseBuffer: %w", err)
		}
	}

	out := make([]int16, wantSamples)
	copy(out, (*pending)[:wantSamples])
	*pending = append([]int16{}, (*pending)[wantSamples:]...)
	return Frame{Samples: out}, nil
}

// Format implements Source.
func (s *wasapiCaptureSource) Format() Format { return s.format }

// ReadFrame implements Source by delegating to the COM-pinned capture
// goroutine and waiting for its reply (or ctx cancellation).
func (s *wasapiCaptureSource) ReadFrame(ctx context.Context) (Frame, error) {
	reply := make(chan captureReply, 1)
	select {
	case s.reqCh <- captureCmd{ctx: ctx, reply: reply}:
	case <-s.closeCh:
		return Frame{}, ErrOSAudioUnavailable
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	}
	select {
	case r := <-reply:
		return r.frame, r.err
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	}
}

// Close stops the capture stream and releases the underlying WASAPI objects.
// Safe to call multiple times.
func (s *wasapiCaptureSource) Close() error {
	s.closeOnce.Do(func() { close(s.closeCh) })
	<-s.doneCh
	return nil
}

// ---------------------------------------------------------------------------
// Playback (speaker) — wasapiPlaybackSink implements Sink.
// ---------------------------------------------------------------------------

type renderCmd struct {
	frame Frame
	reply chan error
}

// wasapiPlaybackSink is a real Sink backed by a WASAPI render (speaker)
// endpoint, mirroring wasapiCaptureSource's COM-thread-affinity design.
type wasapiPlaybackSink struct {
	format Format

	reqCh   chan renderCmd
	closeCh chan struct{}
	doneCh  chan struct{}

	closeOnce sync.Once
}

// NewOSPlaybackSink opens the system default speaker via WASAPI in shared
// mode at f's sample rate as 16-bit PCM, negotiating the device's native
// channel count (see the file doc comment) and converting from f.Channels in
// software. It returns ErrNoAudioDevice if WASAPI reports no render endpoint
// exists at all, or ErrSampleRateMismatch if the device's native rate differs
// from f.SampleRate.
func NewOSPlaybackSink(f Format) (Sink, error) {
	if f.Channels == 0 {
		f.Channels = 1
	}
	if f.SampleRate == 0 {
		return nil, fmt.Errorf("audio: NewOSPlaybackSink: format has zero SampleRate")
	}

	type initResult struct{ err error }
	initDone := make(chan initResult, 1)

	snk := &wasapiPlaybackSink{
		format:  f,
		reqCh:   make(chan renderCmd),
		closeCh: make(chan struct{}),
		doneCh:  make(chan struct{}),
	}

	go func() {
		defer close(snk.doneCh)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err == nil {
			defer ole.CoUninitialize()
		}

		mmde, device, err := defaultEndpoint(wca.ERender)
		if err != nil {
			initDone <- initResult{err: err}
			return
		}
		defer mmde.Release()
		defer device.Release()

		ac, nf, bufferFrames, err := openNegotiatedClient(device, f, 0)
		if err != nil {
			initDone <- initResult{err: err}
			return
		}
		defer ac.Release()

		var arc *wca.IAudioRenderClient
		if err := ac.GetService(wca.IID_IAudioRenderClient, &arc); err != nil {
			initDone <- initResult{err: fmt.Errorf("audio: WASAPI IAudioClient.GetService(IAudioRenderClient): %w", err)}
			return
		}
		defer arc.Release()

		deviceChannels := nf.deviceChannels()

		// Prime the full buffer with silence before Start so the engine never
		// reads uninitialized/underrun memory on the first tick.
		if err := renderSilence(arc, bufferFrames, deviceChannels); err != nil {
			initDone <- initResult{err: err}
			return
		}
		if err := ac.Start(); err != nil {
			initDone <- initResult{err: fmt.Errorf("audio: WASAPI IAudioClient.Start: %w", err)}
			return
		}
		defer ac.Stop()

		initDone <- initResult{}

		appChannels := int(f.Channels)
		for {
			select {
			case <-snk.closeCh:
				return
			case cmd := <-snk.reqCh:
				err := renderLoopWriteFrame(ac, arc, appChannels, deviceChannels, bufferFrames, cmd.frame)
				select {
				case cmd.reply <- err:
				case <-snk.closeCh:
					return
				}
				if err != nil {
					return
				}
			}
		}
	}()

	res := <-initDone
	if res.err != nil {
		<-snk.doneCh
		return nil, res.err
	}
	return snk, nil
}

// renderSilence fills the entire allocated render buffer (deviceChannels
// layout) with silence. Used to prime the stream before Start.
func renderSilence(arc *wca.IAudioRenderClient, bufferFrames uint32, deviceChannels int) error {
	if bufferFrames == 0 {
		return nil
	}
	var data *byte
	if err := arc.GetBuffer(bufferFrames, &data); err != nil {
		return fmt.Errorf("audio: WASAPI IAudioRenderClient.GetBuffer(prime): %w", err)
	}
	if data != nil {
		n := int(bufferFrames) * deviceChannels
		samples := unsafe.Slice((*int16)(unsafe.Pointer(data)), n)
		for i := range samples {
			samples[i] = 0
		}
	}
	if err := arc.ReleaseBuffer(bufferFrames, wca.AUDCLNT_BUFFERFLAGS_SILENT); err != nil {
		return fmt.Errorf("audio: WASAPI IAudioRenderClient.ReleaseBuffer(prime): %w", err)
	}
	return nil
}

// renderLoopWriteFrame upmixes/downmixes one Frame's samples from appChannels
// to deviceChannels and writes them to the render endpoint, waiting for
// enough free space in the device's buffer (via GetCurrentPadding) first.
func renderLoopWriteFrame(ac *wca.IAudioClient, arc *wca.IAudioRenderClient, appChannels, deviceChannels int, bufferFrames uint32, frame Frame) error {
	if appChannels == 0 {
		appChannels = 1
	}
	deviceSamples := upmixFromApp(frame.Samples, appChannels, deviceChannels)
	framesNeeded := uint32(len(deviceSamples) / max(deviceChannels, 1))
	if framesNeeded == 0 {
		return nil
	}

	// Wait until there is room for the whole frame. Shared-mode buffers here
	// are sized generously (wasapiBufferDuration, 200ms) relative to one
	// SamplesPerFrame (10ms) frame, so this loop is expected to pass through
	// quickly; it exists to avoid AUDCLNT_E_BUFFER_TOO_LARGE on a slow consumer.
	for {
		var padding uint32
		if err := ac.GetCurrentPadding(&padding); err != nil {
			return fmt.Errorf("audio: WASAPI IAudioClient.GetCurrentPadding: %w", err)
		}
		free := bufferFrames - padding
		if free >= framesNeeded {
			break
		}
		if !sleepOrDone(context.Background(), nil) {
			break
		}
	}

	var data *byte
	if err := arc.GetBuffer(framesNeeded, &data); err != nil {
		return fmt.Errorf("audio: WASAPI IAudioRenderClient.GetBuffer: %w", err)
	}
	if data != nil {
		samples := unsafe.Slice((*int16)(unsafe.Pointer(data)), len(deviceSamples))
		copy(samples, deviceSamples)
	}
	if err := arc.ReleaseBuffer(framesNeeded, 0); err != nil {
		return fmt.Errorf("audio: WASAPI IAudioRenderClient.ReleaseBuffer: %w", err)
	}
	return nil
}

// Format implements Sink.
func (s *wasapiPlaybackSink) Format() Format { return s.format }

// WriteFrame implements Sink by delegating to the COM-pinned render goroutine.
func (s *wasapiPlaybackSink) WriteFrame(f Frame) error {
	reply := make(chan error, 1)
	select {
	case s.reqCh <- renderCmd{frame: f, reply: reply}:
	case <-s.closeCh:
		return ErrOSAudioUnavailable
	}
	return <-reply
}

// Close stops the playback stream and releases the underlying WASAPI objects.
// Safe to call multiple times.
func (s *wasapiPlaybackSink) Close() error {
	s.closeOnce.Do(func() { close(s.closeCh) })
	<-s.doneCh
	return nil
}

// ---------------------------------------------------------------------------
// Shared helpers.
// ---------------------------------------------------------------------------

// wasapiPollInterval is how often the capture/render loops re-check WASAPI
// when waiting for data or buffer space, in the absence of the event-driven
// (SetEventHandle + WaitForSingleObject) mode. It is small relative to a
// SamplesPerFrame (10ms @ 48kHz) frame so it does not itself become a source
// of added latency or jitter.
const wasapiPollInterval = 2 * time.Millisecond

// sleepOrDone waits one poll interval, returning false if ctx is cancelled or
// stop is closed first (stop may be nil, meaning "no stop channel"). true
// means the sleep completed normally and the caller should retry its check.
func sleepOrDone(ctx context.Context, stop <-chan struct{}) bool {
	t := time.NewTimer(wasapiPollInterval)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	case <-stop:
		return false
	}
}
