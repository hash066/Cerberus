//go:build darwin && !(cgo && cerberus_coreaudio)

package audio

import (
	"errors"
	"testing"
)

// A DEFAULT macOS build has no verified backend wired in (see os_darwin_stub.go
// for why the CoreAudio implementation is opt-in), so both stream constructors
// must report ErrOSAudioUnavailable — honestly and unconditionally — rather than
// fabricating a device. This is the test that actually runs on a macOS CI runner.
func TestOSBackendsAreStubs_darwin(t *testing.T) {
	f := testFormat()
	if _, err := NewOSCaptureSource(f); !errors.Is(err, ErrOSAudioUnavailable) {
		t.Fatalf("default macOS build must report the stub error, got %v", err)
	}
	if _, err := NewOSPlaybackSink(f); !errors.Is(err, ErrOSAudioUnavailable) {
		t.Fatalf("default macOS build must report the stub error, got %v", err)
	}
}

// EnumerateEndpoints is a listing query, not a stream open, so on a build with
// no backend it returns an honest empty list rather than an error — the
// deliberate asymmetry documented in os_other.go and audio.go.
func TestEnumerateEndpointsIsEmptyStub_darwin(t *testing.T) {
	endpoints, err := EnumerateEndpoints()
	if err != nil {
		t.Fatalf("EnumerateEndpoints must not error on a build with no backend, got %v", err)
	}
	if len(endpoints) != 0 {
		t.Fatalf("expected an empty list on a build with no backend, got %d entries", len(endpoints))
	}
}
