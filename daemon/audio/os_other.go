//go:build !windows && !darwin && !linux

// This file is the fallback half of the OS capture/playback boundary: the
// platforms with no real backend wired in.
//
// Real backends exist for Windows (os_windows.go, WASAPI via go-wca), Linux
// (os_linux.go, the PulseAudio native protocol via jfreymuth/pulse — which also
// covers PipeWire through pipewire-pulse), and macOS (os_darwin.go, CoreAudio;
// see that file for its verification status). Everything else — the BSDs,
// Solaris, js/wasm, plan9 — lands here.
//
// Per CLAUDE.md "Maturity honesty", this is a documented stub, not a faked
// working backend: the constructors return ErrOSAudioUnavailable so a caller
// that asks for a real device fails loudly instead of silently producing fake
// audio. SineSource / BufferSink remain the standalone-buildable, testable path
// (see audio.go).
//
// Adding a platform here is a matter of implementing the same three functions;
// the rest of the package neither knows nor cares which backend it got.
package audio

// NewOSCaptureSource is the entry point for a real OS microphone-capture
// Source. This platform has no backend wired in, so it returns
// ErrOSAudioUnavailable rather than faking a working mic.
func NewOSCaptureSource(_ Format) (Source, error) {
	return nil, ErrOSAudioUnavailable
}

// NewOSPlaybackSink is the entry point for a real OS speaker-playback Sink.
// This platform has no backend wired in, so it returns ErrOSAudioUnavailable
// rather than faking a working speaker.
func NewOSPlaybackSink(_ Format) (Sink, error) {
	return nil, ErrOSAudioUnavailable
}

// EnumerateEndpoints is the entry point for real OS audio-device enumeration.
// No backend is wired in for this platform, so it returns an empty list and a
// nil error rather than fabricating devices.
//
// The asymmetry with NewOSCaptureSource/NewOSPlaybackSink above is deliberate:
// enumeration is a LISTING query, and "no devices are known on this platform"
// is a valid — if disappointing — list. The stream constructors fail loudly
// instead, because a caller asking for a specific device wants that device, and
// handing it fabricated silence would be a lie. Note that the platforms WITH a
// backend do report an error (ErrNoAudioDevice) when the machine genuinely has
// no endpoints; that is a different question ("this machine has no microphone")
// from the one answered here ("this build cannot see microphones at all").
func EnumerateEndpoints() ([]EndpointInfo, error) {
	return nil, nil
}
