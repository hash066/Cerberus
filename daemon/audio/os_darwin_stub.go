//go:build darwin && !(cgo && cerberus_coreaudio)

// This file is what a DEFAULT macOS build of this package gets: the honest
// stub. It is selected unless a build explicitly opts into the CoreAudio
// backend with both cgo enabled and the `cerberus_coreaudio` build tag.
//
// # Why the real backend is opt-in rather than the default
//
// os_darwin.go contains a complete CoreAudio (AudioQueue) implementation of the
// three OS entry points. It has NEVER BEEN COMPILED AND NEVER BEEN RUN — no Mac
// and no macOS SDK/cross-toolchain were reachable from the environment it was
// written in, so not one line of it has been through a compiler, let alone
// played a sound. Shipping unverified code as the DEFAULT would be the exact
// "faking it works" that CLAUDE.md's maturity-honesty rule forbids, and would
// break the moment anyone ran `go build ./...` on a Mac (where cgo is on by
// default) or added a macOS runner to CI.
//
// So the default is this stub, which is truthful and cannot break anyone:
//
//	GOOS=darwin (any CGO setting), no tag  -> this file, ErrOSAudioUnavailable
//	GOOS=darwin CGO_ENABLED=1 -tags cerberus_coreaudio -> os_darwin.go (unverified)
//
// Notably this keeps the CGO_ENABLED=0 cross-compile release path
// (build/release.sh) building darwin/amd64 and darwin/arm64 exactly as it does
// today — macOS binaries simply have no audio backend, which is the status quo
// and is reported honestly at runtime rather than crashing or faking silence.
//
// # How to finish the macOS backend (for whoever has a Mac)
//
//  1. go build -tags cerberus_coreaudio ./daemon/audio/   — expect compile
//     errors; nobody has ever run this step.
//  2. go test  -tags cerberus_coreaudio ./daemon/audio/   — os_darwin_test.go
//     has the live mic/speaker tests, mirroring os_windows_test.go and
//     os_linux_test.go.
//  3. Once it genuinely captures and plays on real hardware, delete this file
//     and change os_darwin.go's constraint from
//     `darwin && cgo && cerberus_coreaudio` to `darwin && cgo`, adding a
//     `darwin && !cgo` stub so CGO_ENABLED=0 darwin cross-builds keep working.
//     At that point Lane P must switch the darwin release build to a native
//     macOS runner with CGO_ENABLED=1, because a cgo backend cannot be
//     cross-compiled from Linux — see this lane's report.
package audio

// NewOSCaptureSource is the entry point for a real OS microphone-capture
// Source. The default macOS build has no verified backend wired in, so it
// returns ErrOSAudioUnavailable rather than faking a working mic. See this
// file's doc comment for the opt-in CoreAudio path.
func NewOSCaptureSource(_ Format) (Source, error) {
	return nil, ErrOSAudioUnavailable
}

// NewOSPlaybackSink is the entry point for a real OS speaker-playback Sink. The
// default macOS build has no verified backend wired in, so it returns
// ErrOSAudioUnavailable rather than faking a working speaker.
func NewOSPlaybackSink(_ Format) (Sink, error) {
	return nil, ErrOSAudioUnavailable
}

// EnumerateEndpoints is the entry point for real OS audio-device enumeration.
// The default macOS build has no verified backend, so it returns an empty list
// and a nil error rather than fabricating devices — the same listing-query
// asymmetry os_other.go documents.
func EnumerateEndpoints() ([]EndpointInfo, error) {
	return nil, nil
}
