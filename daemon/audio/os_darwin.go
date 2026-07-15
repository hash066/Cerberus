//go:build darwin && cgo && cerberus_coreaudio

// This file is the macOS half of the OS capture/playback boundary described in
// audio.go: a CoreAudio (AudioQueue) Source/Sink pair.
//
// ############################################################################
// #                                                                          #
// #  STATUS: NEVER COMPILED. NEVER RUN. NOT HARDWARE-VERIFIED.               #
// #                                                                          #
// #  No Mac, no macOS SDK, and no darwin cross-toolchain were reachable from #
// #  the environment this was written in (checked: no Mac on the LAN, no ssh #
// #  target, no osxcross/zig/clang, no MacOSX*.sdk on disk). Not one line of #
// #  this file has been through a compiler, let alone captured or played a   #
// #  sample. Do NOT present macOS audio as working on the strength of this   #
// #  file existing (CLAUDE.md, "Maturity honesty").                          #
// #                                                                          #
// #  That is why it is behind the `cerberus_coreaudio` build tag and is NOT  #
// #  part of any default build: see os_darwin_stub.go, which is what a       #
// #  normal macOS build gets, and which documents the promotion steps for    #
// #  whoever has a Mac. Expect compile errors on the first attempt.          #
// #                                                                          #
// ############################################################################
//
// # Design (mirrors os_windows.go / os_linux.go so the three read alike)
//
// AudioQueue is chosen over AudioUnit/AUHAL deliberately. AudioQueue does its
// own buffering and, crucially, its own format conversion: it accepts a client
// format and reconciles it with the device's native format internally, which is
// exactly the negotiation os_windows.go has to hand-roll (see its mono<->stereo
// downmix) and that PulseAudio does server-side on Linux. AUHAL would mean
// owning the device format, the render callback's threading, and the conversion
// by hand for no benefit at this package's frame sizes.
//
// Sample-rate policy follows os_linux.go rather than os_windows.go: CoreAudio's
// conversion is a real, first-class implementation, not the naive resampling
// os_windows.go refuses to write, so a rate the device does not natively run at
// is a normal request rather than ErrSampleRateMismatch.
//
// # cgo, and what it costs
//
// There is no pure-Go path to CoreAudio: it is a C framework reached through
// the dynamic linker, with no wire protocol to reimplement (contrast Linux,
// where PulseAudio's socket protocol let os_linux.go stay pure Go, and Windows,
// where go-wca drives COM vtables over syscall). So this file is the ONLY cgo
// in daemon/audio, and it is why it must stay opt-in: cgo cannot be
// cross-compiled from the Linux release runner without a macOS SDK, so shipping
// it by default would break build/release.sh's CGO_ENABLED=0 darwin targets.
// See this lane's report for exactly what Lane P would need to change.
package audio

/*
#cgo LDFLAGS: -framework CoreFoundation -framework CoreAudio -framework AudioToolbox

#include <stdlib.h>
#include <string.h>
#include <CoreFoundation/CoreFoundation.h>
#include <CoreAudio/CoreAudio.h>
#include <AudioToolbox/AudioToolbox.h>

// Implemented in Go (os_darwin_export.go, //export). Declared here so the
// wrappers below can take their address; this file holds no //export itself,
// which is what lets it carry the static definitions below at all.
extern void cerberusAudioCaptureCB(void *ud, AudioQueueRef aq, AudioQueueBufferRef buf,
                                   const AudioTimeStamp *ts, UInt32 nPackets,
                                   const AudioStreamPacketDescription *descs);
extern void cerberusAudioRenderCB(void *ud, AudioQueueRef aq, AudioQueueBufferRef buf);

// Thin wrappers so Go never has to form a C function pointer itself.
static OSStatus cerberusNewInputQueue(AudioStreamBasicDescription *fmt, void *ud, AudioQueueRef *out) {
	return AudioQueueNewInput(fmt, cerberusAudioCaptureCB, ud, NULL, NULL, 0, out);
}

static OSStatus cerberusNewOutputQueue(AudioStreamBasicDescription *fmt, void *ud, AudioQueueRef *out) {
	return AudioQueueNewOutput(fmt, cerberusAudioRenderCB, ud, NULL, NULL, 0, out);
}

// Property addresses are small structs passed by pointer; building them in C
// keeps the field-name churn (kAudioObjectPropertyElementMain vs the
// deprecated ...Master) in one place.
static AudioObjectPropertyAddress cerberusAddr(AudioObjectPropertySelector sel, AudioObjectPropertyScope scope) {
	AudioObjectPropertyAddress a;
	a.mSelector = sel;
	a.mScope    = scope;
	a.mElement  = kAudioObjectPropertyElementMain;
	return a;
}

// cerberusChannelsInScope sums the channel count the device exposes in the
// given scope (input or output). A device with 0 input channels is not a
// microphone; a device with 0 output channels is not a speaker. This is the
// structural way to classify an endpoint, rather than guessing from its name.
static UInt32 cerberusChannelsInScope(AudioObjectID dev, AudioObjectPropertyScope scope) {
	AudioObjectPropertyAddress addr = cerberusAddr(kAudioDevicePropertyStreamConfiguration, scope);
	UInt32 size = 0;
	if (AudioObjectGetPropertyDataSize(dev, &addr, 0, NULL, &size) != noErr || size == 0) {
		return 0;
	}
	AudioBufferList *bl = (AudioBufferList *)malloc(size);
	if (bl == NULL) {
		return 0;
	}
	if (AudioObjectGetPropertyData(dev, &addr, 0, NULL, &size, bl) != noErr) {
		free(bl);
		return 0;
	}
	UInt32 total = 0;
	for (UInt32 i = 0; i < bl->mNumberBuffers; i++) {
		total += bl->mBuffers[i].mNumberChannels;
	}
	free(bl);
	return total;
}

// cerberusDeviceName copies the device's human name into buf. Returns 1 on
// success. CFString -> UTF-8 is done here so Go never touches a CFStringRef.
static int cerberusDeviceName(AudioObjectID dev, char *buf, int cap) {
	AudioObjectPropertyAddress addr = cerberusAddr(kAudioObjectPropertyName, kAudioObjectPropertyScopeGlobal);
	CFStringRef name = NULL;
	UInt32 size = sizeof(name);
	if (AudioObjectGetPropertyData(dev, &addr, 0, NULL, &size, &name) != noErr || name == NULL) {
		return 0;
	}
	int ok = CFStringGetCString(name, buf, cap, kCFStringEncodingUTF8) ? 1 : 0;
	CFRelease(name);
	return ok;
}

// cerberusDefaultDevice returns the system default input or output device.
static AudioObjectID cerberusDefaultDevice(int input) {
	AudioObjectPropertyAddress addr = cerberusAddr(
		input ? kAudioHardwarePropertyDefaultInputDevice : kAudioHardwarePropertyDefaultOutputDevice,
		kAudioObjectPropertyScopeGlobal);
	AudioObjectID dev = kAudioObjectUnknown;
	UInt32 size = sizeof(dev);
	if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &addr, 0, NULL, &size, &dev) != noErr) {
		return kAudioObjectUnknown;
	}
	return dev;
}

// cerberusDeviceList fills ids with up to cap device IDs, returning the count.
static int cerberusDeviceList(AudioObjectID *ids, int cap) {
	AudioObjectPropertyAddress addr = cerberusAddr(kAudioHardwarePropertyDevices, kAudioObjectPropertyScopeGlobal);
	UInt32 size = 0;
	if (AudioObjectGetPropertyDataSize(kAudioObjectSystemObject, &addr, 0, NULL, &size) != noErr) {
		return -1;
	}
	int n = (int)(size / sizeof(AudioObjectID));
	if (n > cap) {
		n = cap;
	}
	if (n <= 0) {
		return 0;
	}
	size = (UInt32)(n * (int)sizeof(AudioObjectID));
	if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &addr, 0, NULL, &size, ids) != noErr) {
		return -1;
	}
	return n;
}
*/
import "C"

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unsafe"
)

const (
	// coreAudioBuffers is how many AudioQueue buffers are kept in flight. Three
	// is the conventional minimum that keeps the device fed while one buffer is
	// being filled/drained and one is in the caller's hands.
	coreAudioBuffers = 3

	// coreAudioCaptureQueueFrames / coreAudioPlaybackQueueFrames mirror the
	// Linux backend's hand-off queue depths; see os_linux.go for the reasoning
	// (capture cannot apply backpressure and so drops oldest; playback can and
	// so blocks).
	coreAudioCaptureQueueFrames  = 32
	coreAudioPlaybackQueueFrames = 8

	// coreAudioUnderrunWait mirrors pulseUnderrunWait: how long the render
	// callback waits for real audio before concealing a gap with silence.
	coreAudioUnderrunWait = 10 * time.Millisecond

	// coreAudioWriteTimeout bounds WriteFrame's block on a full queue, so a dead
	// queue surfaces as an error instead of hanging the caller.
	coreAudioWriteTimeout = time.Second

	// coreAudioMaxDevices caps the enumeration buffer. Real machines have a
	// handful of endpoints; this is a sanity bound, not a real limit.
	coreAudioMaxDevices = 64
)

// ---------------------------------------------------------------------------
// Handle registry.
//
// cgo forbids passing a Go pointer to C and getting it back later, so the
// AudioQueue userData is an integer handle into these maps rather than a
// *coreAudioCaptureSource. The exported callbacks (os_darwin_export.go) look
// the stream back up by that handle.
// ---------------------------------------------------------------------------

var (
	coreAudioMu       sync.Mutex
	coreAudioNextID   uintptr
	coreAudioCaptures = map[uintptr]*coreAudioCaptureSource{}
	coreAudioRenders  = map[uintptr]*coreAudioPlaybackSink{}
)

func coreAudioRegisterCapture(s *coreAudioCaptureSource) uintptr {
	coreAudioMu.Lock()
	defer coreAudioMu.Unlock()
	coreAudioNextID++
	id := coreAudioNextID
	coreAudioCaptures[id] = s
	return id
}

func coreAudioRegisterRender(s *coreAudioPlaybackSink) uintptr {
	coreAudioMu.Lock()
	defer coreAudioMu.Unlock()
	coreAudioNextID++
	id := coreAudioNextID
	coreAudioRenders[id] = s
	return id
}

func coreAudioLookupCapture(id uintptr) *coreAudioCaptureSource {
	coreAudioMu.Lock()
	defer coreAudioMu.Unlock()
	return coreAudioCaptures[id]
}

func coreAudioLookupRender(id uintptr) *coreAudioPlaybackSink {
	coreAudioMu.Lock()
	defer coreAudioMu.Unlock()
	return coreAudioRenders[id]
}

func coreAudioUnregister(id uintptr) {
	coreAudioMu.Lock()
	defer coreAudioMu.Unlock()
	delete(coreAudioCaptures, id)
	delete(coreAudioRenders, id)
}

// ---------------------------------------------------------------------------
// Format helpers.
// ---------------------------------------------------------------------------

// coreAudioASBD builds the AudioStreamBasicDescription for f: 16-bit signed,
// packed, interleaved little-endian PCM — the exact layout audio.Frame carries,
// so no conversion happens on this side of the boundary.
func coreAudioASBD(f Format) C.AudioStreamBasicDescription {
	bytesPerFrame := C.UInt32(f.Channels) * 2
	return C.AudioStreamBasicDescription{
		mSampleRate:       C.Float64(f.SampleRate),
		mFormatID:         C.kAudioFormatLinearPCM,
		mFormatFlags:      C.kLinearPCMFormatFlagIsSignedInteger | C.kLinearPCMFormatFlagIsPacked,
		mBytesPerPacket:   bytesPerFrame,
		mFramesPerPacket:  1,
		mBytesPerFrame:    bytesPerFrame,
		mChannelsPerFrame: C.UInt32(f.Channels),
		mBitsPerChannel:   16,
	}
}

// normalizeCoreAudioFormat applies the same defaulting rules as the other two
// backends: absent channel count means mono, a zero sample rate is a caller bug.
func normalizeCoreAudioFormat(f Format, who string) (Format, error) {
	if f.Channels == 0 {
		f.Channels = 1
	}
	if f.SampleRate == 0 {
		return f, fmt.Errorf("audio: %s: format has zero SampleRate", who)
	}
	return f, nil
}

// coreAudioErr turns a non-zero OSStatus into an error naming the call site.
func coreAudioErr(who string, st C.OSStatus) error {
	if st == C.noErr {
		return nil
	}
	return fmt.Errorf("audio: CoreAudio %s failed: OSStatus %d", who, int32(st))
}

// coreAudioBufferBytes is the byte size of one AudioQueue buffer: exactly one
// SamplesPerFrame frame, matching the frame this package hands around.
func coreAudioBufferBytes(f Format) C.UInt32 {
	return C.UInt32(f.SamplesPerPacket() * 2)
}

// ---------------------------------------------------------------------------
// EnumerateEndpoints.
// ---------------------------------------------------------------------------

// EnumerateEndpoints lists every microphone and speaker CoreAudio reports,
// ordered mics-then-speakers to match the other backends so all three platforms
// index /cer/dev/audio/<kind>/<index> the same way.
//
// Endpoints are classified structurally by channel count per scope, not by
// name: a device with input channels is a mic, one with output channels is a
// speaker, and an aggregate device with both is BOTH (it is listed once per
// kind), which is the honest description of e.g. a USB headset.
//
// Returns ErrNoAudioDevice if the machine has no endpoint of either kind.
func EnumerateEndpoints() ([]EndpointInfo, error) {
	ids := make([]C.AudioObjectID, coreAudioMaxDevices)
	n := int(C.cerberusDeviceList(&ids[0], C.int(coreAudioMaxDevices)))
	if n < 0 {
		return nil, fmt.Errorf("audio: CoreAudio kAudioHardwarePropertyDevices query failed")
	}

	var mics, speakers []EndpointInfo
	for i := 0; i < n; i++ {
		dev := ids[i]
		in := uint32(C.cerberusChannelsInScope(dev, C.kAudioObjectPropertyScopeInput))
		out := uint32(C.cerberusChannelsInScope(dev, C.kAudioObjectPropertyScopeOutput))
		if in == 0 && out == 0 {
			continue // neither a mic nor a speaker (e.g. a control-only device)
		}
		name := coreAudioName(dev)
		if in > 0 {
			mics = append(mics, EndpointInfo{Name: coreAudioLabel(name, EndpointMic, len(mics)), Kind: EndpointMic})
		}
		if out > 0 {
			speakers = append(speakers, EndpointInfo{Name: coreAudioLabel(name, EndpointSpeaker, len(speakers)), Kind: EndpointSpeaker})
		}
	}
	if len(mics) == 0 && len(speakers) == 0 {
		return nil, ErrNoAudioDevice
	}
	return append(mics, speakers...), nil
}

// coreAudioName reads a device's human-readable name, or "" if CoreAudio cannot
// describe it.
func coreAudioName(dev C.AudioObjectID) string {
	buf := make([]C.char, 256)
	if C.cerberusDeviceName(dev, &buf[0], C.int(len(buf))) == 0 {
		return ""
	}
	return C.GoString(&buf[0])
}

// coreAudioLabel mirrors endpointFriendlyName/pulseEndpointName: prefer the
// real name, else a stable synthetic one — never drop a real device just
// because the OS could not label it.
func coreAudioLabel(name string, kind EndpointKind, index int) string {
	if name != "" {
		return name
	}
	return fmt.Sprintf("%s %d", kind, index)
}

// ---------------------------------------------------------------------------
// Capture (microphone) — coreAudioCaptureSource implements Source.
// ---------------------------------------------------------------------------

// coreAudioCaptureSource is a Source backed by an AudioQueue input queue.
// AudioQueue PUSHES filled buffers into a callback on its own thread while the
// Source interface is PULL-based, so the callback re-slices into exact frames
// and hands them over a bounded channel — the same shape as os_linux.go.
type coreAudioCaptureSource struct {
	format Format
	id     uintptr
	queue  C.AudioQueueRef

	frames chan Frame

	mu      sync.Mutex
	pending []int16
	dropped uint64

	closeCh   chan struct{}
	closeOnce sync.Once
}

// NewOSCaptureSource opens the system default microphone via AudioQueue at f's
// sample rate and channel count as 16-bit PCM. Returns ErrNoAudioDevice if the
// machine has no default input device.
func NewOSCaptureSource(f Format) (Source, error) {
	f, err := normalizeCoreAudioFormat(f, "NewOSCaptureSource")
	if err != nil {
		return nil, err
	}
	if C.cerberusDefaultDevice(1) == C.kAudioObjectUnknown {
		return nil, fmt.Errorf("%w: no default input device", ErrNoAudioDevice)
	}

	s := &coreAudioCaptureSource{
		format:  f,
		frames:  make(chan Frame, coreAudioCaptureQueueFrames),
		closeCh: make(chan struct{}),
	}
	s.id = coreAudioRegisterCapture(s)

	asbd := coreAudioASBD(f)
	if st := C.cerberusNewInputQueue(&asbd, unsafe.Pointer(s.id), &s.queue); st != C.noErr {
		coreAudioUnregister(s.id)
		return nil, coreAudioErr("AudioQueueNewInput", st)
	}

	size := coreAudioBufferBytes(f)
	for i := 0; i < coreAudioBuffers; i++ {
		var buf C.AudioQueueBufferRef
		if st := C.AudioQueueAllocateBuffer(s.queue, size, &buf); st != C.noErr {
			C.AudioQueueDispose(s.queue, C.true)
			coreAudioUnregister(s.id)
			return nil, coreAudioErr("AudioQueueAllocateBuffer(input)", st)
		}
		if st := C.AudioQueueEnqueueBuffer(s.queue, buf, 0, nil); st != C.noErr {
			C.AudioQueueDispose(s.queue, C.true)
			coreAudioUnregister(s.id)
			return nil, coreAudioErr("AudioQueueEnqueueBuffer(input)", st)
		}
	}
	if st := C.AudioQueueStart(s.queue, nil); st != C.noErr {
		C.AudioQueueDispose(s.queue, C.true)
		coreAudioUnregister(s.id)
		return nil, coreAudioErr("AudioQueueStart(input)", st)
	}
	return s, nil
}

// deliver is called from the AudioQueue capture callback with one buffer's
// worth of samples. It must not block: it runs on CoreAudio's thread.
func (s *coreAudioCaptureSource) deliver(samples []int16) {
	per := s.format.SamplesPerPacket()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.pending = append(s.pending, samples...)
	off := 0
	for len(s.pending)-off >= per {
		frame := Frame{Samples: append([]int16(nil), s.pending[off:off+per]...)}
		off += per
		s.enqueue(frame)
	}
	if off > 0 {
		rem := copy(s.pending, s.pending[off:])
		s.pending = s.pending[:rem]
	}
}

// enqueue hands one frame to ReadFrame, dropping the oldest if the consumer has
// fallen behind (capture has no backpressure — the mic does not wait). Caller
// holds s.mu. Never blocks.
func (s *coreAudioCaptureSource) enqueue(f Frame) {
	select {
	case s.frames <- f:
		return
	default:
	}
	select {
	case <-s.frames:
		s.dropped++
	default:
	}
	select {
	case s.frames <- f:
	default:
		s.dropped++
	}
}

// Dropped reports how many captured frames were discarded because the consumer
// did not keep up; a healthy live capture reports zero.
func (s *coreAudioCaptureSource) Dropped() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Format implements Source.
func (s *coreAudioCaptureSource) Format() Format { return s.format }

// ReadFrame implements Source.
func (s *coreAudioCaptureSource) ReadFrame(ctx context.Context) (Frame, error) {
	select {
	case f := <-s.frames:
		return f, nil
	case <-s.closeCh:
		return Frame{}, ErrOSAudioUnavailable
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	}
}

// Close stops the queue and releases it. Safe to call multiple times.
func (s *coreAudioCaptureSource) Close() error {
	s.closeOnce.Do(func() {
		close(s.closeCh)
		C.AudioQueueStop(s.queue, C.true)
		C.AudioQueueDispose(s.queue, C.true)
		coreAudioUnregister(s.id)
	})
	return nil
}

// ---------------------------------------------------------------------------
// Playback (speaker) — coreAudioPlaybackSink implements Sink.
// ---------------------------------------------------------------------------

// coreAudioPlaybackSink is a Sink backed by an AudioQueue output queue.
// AudioQueue PULLS by handing back drained buffers to refill; WriteFrame
// queues frames for that callback to consume.
type coreAudioPlaybackSink struct {
	format Format
	id     uintptr
	queue  C.AudioQueueRef

	frames chan Frame

	// pending is the tail of a frame the last refill could not fit. Touched only
	// from the AudioQueue callback thread.
	pending []int16

	closeCh   chan struct{}
	closeOnce sync.Once
	startOnce sync.Once

	mu        sync.Mutex
	underruns uint64
}

// NewOSPlaybackSink opens the system default speaker via AudioQueue at f's
// sample rate and channel count as 16-bit PCM. Returns ErrNoAudioDevice if the
// machine has no default output device.
func NewOSPlaybackSink(f Format) (Sink, error) {
	f, err := normalizeCoreAudioFormat(f, "NewOSPlaybackSink")
	if err != nil {
		return nil, err
	}
	if C.cerberusDefaultDevice(0) == C.kAudioObjectUnknown {
		return nil, fmt.Errorf("%w: no default output device", ErrNoAudioDevice)
	}

	s := &coreAudioPlaybackSink{
		format:  f,
		frames:  make(chan Frame, coreAudioPlaybackQueueFrames),
		closeCh: make(chan struct{}),
	}
	s.id = coreAudioRegisterRender(s)

	asbd := coreAudioASBD(f)
	if st := C.cerberusNewOutputQueue(&asbd, unsafe.Pointer(s.id), &s.queue); st != C.noErr {
		coreAudioUnregister(s.id)
		return nil, coreAudioErr("AudioQueueNewOutput", st)
	}
	return s, nil
}

// primeAndStart allocates the output buffers and starts the queue on the first
// WriteFrame. AudioQueue only calls the render callback for buffers that have
// been enqueued, so the buffers are filled once here to kick the cycle off;
// after that each callback refills and re-enqueues its own buffer.
func (s *coreAudioPlaybackSink) primeAndStart() error {
	size := coreAudioBufferBytes(s.format)
	for i := 0; i < coreAudioBuffers; i++ {
		var buf C.AudioQueueBufferRef
		if st := C.AudioQueueAllocateBuffer(s.queue, size, &buf); st != C.noErr {
			return coreAudioErr("AudioQueueAllocateBuffer(output)", st)
		}
		s.refill(buf)
	}
	if st := C.AudioQueueStart(s.queue, nil); st != C.noErr {
		return coreAudioErr("AudioQueueStart(output)", st)
	}
	return nil
}

// refill fills one AudioQueue buffer from the frame queue and enqueues it back.
// Called both to prime and from the render callback.
//
// As on Linux, it must not block indefinitely (that would stall CoreAudio's
// thread), so a starved buffer is filled with silence after coreAudioUnderrunWait
// — the same gap-fill this package's Receiver applies to a lost packet, counted
// so starvation is observable rather than merely audible.
func (s *coreAudioPlaybackSink) refill(buf C.AudioQueueBufferRef) {
	capSamples := int(buf.mAudioDataBytesCapacity) / 2
	out := unsafe.Slice((*int16)(buf.mAudioData), capSamples)

	ch := int(s.format.Channels)
	usable := (capSamples / ch) * ch
	n := 0
	for n < usable {
		if len(s.pending) == 0 {
			select {
			case f := <-s.frames:
				s.pending = f.Samples
				continue
			default:
			}
			if n > 0 {
				break // short but frame-aligned; enqueue what we have
			}
			timer := time.NewTimer(coreAudioUnderrunWait)
			select {
			case f := <-s.frames:
				timer.Stop()
				s.pending = f.Samples
				continue
			case <-s.closeCh:
				timer.Stop()
				for i := 0; i < usable; i++ {
					out[i] = 0
				}
				n = usable
			case <-timer.C:
				s.mu.Lock()
				s.underruns++
				s.mu.Unlock()
				for i := 0; i < usable; i++ {
					out[i] = 0
				}
				n = usable
			}
			break
		}
		c := copy(out[n:usable], s.pending)
		s.pending = s.pending[c:]
		n += c
	}

	buf.mAudioDataByteSize = C.UInt32(n * 2)
	C.AudioQueueEnqueueBuffer(s.queue, buf, 0, nil)
}

// Underruns reports how many times the render callback ran out of audio and
// concealed the gap with silence. As on Linux, expect a small non-zero count
// from the initial prime, and treat sustained growth as the real signal.
func (s *coreAudioPlaybackSink) Underruns() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.underruns
}

// Format implements Sink.
func (s *coreAudioPlaybackSink) Format() Format { return s.format }

// WriteFrame implements Sink by queueing the frame for the render callback,
// blocking while the queue is full so the caller inherits the device's pacing.
// The first call primes the buffers and starts the queue, so a Sink that is
// opened but never fed costs nothing.
func (s *coreAudioPlaybackSink) WriteFrame(f Frame) error {
	ch := int(s.format.Channels)
	if ch > 0 && len(f.Samples)%ch != 0 {
		return fmt.Errorf("audio: NewOSPlaybackSink: frame of %d samples is not a whole number of %d-channel sample-frames", len(f.Samples), ch)
	}
	if len(f.Samples) == 0 {
		return nil
	}
	cp := append([]int16(nil), f.Samples...)

	var startErr error
	queued := false
	s.startOnce.Do(func() {
		// Queue the first frame before priming so the prime sees real audio.
		select {
		case s.frames <- Frame{Samples: cp}:
			queued = true
		default:
		}
		startErr = s.primeAndStart()
	})
	if startErr != nil {
		return startErr
	}
	if queued {
		return nil
	}

	timer := time.NewTimer(coreAudioWriteTimeout)
	defer timer.Stop()
	select {
	case s.frames <- Frame{Samples: cp}:
		return nil
	case <-s.closeCh:
		return ErrOSAudioUnavailable
	case <-timer.C:
		return fmt.Errorf("audio: CoreAudio playback queue stopped consuming (blocked %s)", coreAudioWriteTimeout)
	}
}

// Close stops the queue and releases it. Safe to call multiple times.
func (s *coreAudioPlaybackSink) Close() error {
	s.closeOnce.Do(func() {
		close(s.closeCh)
		C.AudioQueueStop(s.queue, C.true)
		C.AudioQueueDispose(s.queue, C.true)
		coreAudioUnregister(s.id)
	})
	return nil
}
