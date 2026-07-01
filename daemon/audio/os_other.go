//go:build !windows

// This file is the non-Windows half of the OS capture/playback boundary.
//
// A real backend exists for Windows (see os_windows.go, WASAPI via go-wca).
// macOS (CoreAudio) and Linux (PipeWire) backends are NOT implemented here —
// per CLAUDE.md "Maturity honesty", this is a documented stub, not a faked
// working backend. The constructors return ErrOSAudioUnavailable so a caller
// that asks for a real device on these platforms fails loudly instead of
// silently producing fake audio. SineSource / BufferSink remain the
// standalone-buildable, testable path (see audio.go).
package audio

// NewOSCaptureSource is the entry point for a real OS microphone-capture
// Source. On non-Windows builds no backend is wired in yet (TODO: CoreAudio /
// PipeWire). It returns ErrOSAudioUnavailable so nothing fakes a working mic.
func NewOSCaptureSource(_ Format) (Source, error) {
	return nil, ErrOSAudioUnavailable
}

// NewOSPlaybackSink is the entry point for a real OS speaker-playback Sink. On
// non-Windows builds no backend is wired in yet (TODO: CoreAudio / PipeWire).
// It returns ErrOSAudioUnavailable so nothing fakes a working speaker.
func NewOSPlaybackSink(_ Format) (Sink, error) {
	return nil, ErrOSAudioUnavailable
}

// EnumerateEndpoints is the entry point for real OS audio-device enumeration.
// A real backend exists for Windows (see os_windows.go, WASAPI
// IMMDeviceEnumerator). No backend is wired in yet on macOS/Linux, so this
// returns an empty, clearly-labeled list and a nil error rather than
// fabricating devices — per CLAUDE.md "maturity honesty", an empty list is
// the honest answer here (unlike NewOSCaptureSource/NewOSPlaybackSink, which
// fail loudly because a caller asked for a specific stream; enumeration is a
// listing query, and "no devices known on this platform" is a valid, if
// disappointing, list rather than an error).
func EnumerateEndpoints() ([]EndpointInfo, error) {
	return nil, nil
}
